// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestVerifyDrainedIssuerDiscovery(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		rules   []rbacv1.PolicyRule
		allowed bool
	}{
		{name: "default", allowed: true, rules: []rbacv1.PolicyRule{{Verbs: []string{"get"}, NonResourceURLs: []string{"/.well-known/openid-configuration", "/.well-known/openid-configuration/", "/openid/v1/jwks", "/openid/v1/jwks/"}}}},
		{name: "wildcard", rules: []rbacv1.PolicyRule{{Verbs: []string{"get"}, NonResourceURLs: []string{"/openid/*"}}}},
		{name: "secret", rules: []rbacv1.PolicyRule{{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"secrets"}}}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			manifest, admin := fixture(test,
				&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:service-account-issuer-discovery"}, Rules: scenario.rules},
				&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "issuer"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:service-account-issuer-discovery"}, Subjects: []rbacv1.Subject{{Kind: "Group", Name: "system:serviceaccounts"}}},
			)
			err := VerifyDrained(context.Background(), admin, manifest)
			if scenario.allowed && err != nil || !scenario.allowed && (err == nil || !strings.Contains(err.Error(), "role rules exceed")) {
				test.Fatalf("allowed=%t, got %v", scenario.allowed, err)
			}
		})
	}
}

func TestVerifyDrainedGrantReferences(test *testing.T) {
	for _, subject := range []rbacv1.Subject{
		{Kind: "ServiceAccount", Name: "controller", Namespace: "control"},
		{Kind: "User", Name: "system:serviceaccount:control:controller"},
		{Kind: "Group", Name: "system:authenticated"},
		{Kind: "Group", Name: "system:serviceaccounts"},
		{Kind: "Group", Name: "system:serviceaccounts:control"},
		{Kind: "ServiceAccount", Name: "controller", Namespace: "other"},
		{Kind: "User", Name: "unrelated"},
		{Kind: "Group", Name: "system:serviceaccounts:other"},
	} {
		for _, scenario := range []struct {
			name        string
			ref         rbacv1.RoleRef
			missingRole bool
			allowed     bool
		}{
			{name: "safe", ref: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:discovery"}, allowed: true},
			{name: "empty"},
			{name: "wrong-group", ref: rbacv1.RoleRef{APIGroup: "wrong", Kind: "ClusterRole", Name: "system:discovery"}},
			{name: "wrong-kind", ref: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "system:discovery"}},
			{name: "unapproved-name", ref: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:pretend-safe"}},
			{name: "missing-role", ref: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:discovery"}, missingRole: true},
		} {
			test.Run(subject.Kind+"/"+subject.Namespace+subject.Name+"/"+scenario.name, func(test *testing.T) {
				objects := []client.Object{&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "shared"}, Subjects: []rbacv1.Subject{subject}, RoleRef: scenario.ref}}
				if !scenario.missingRole {
					objects = append(objects, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:discovery"}, Rules: []rbacv1.PolicyRule{{Verbs: []string{"get"}, NonResourceURLs: []string{"/api"}}}})
				}
				manifest, admin := fixture(test, objects...)
				err := VerifyDrained(context.Background(), admin, manifest)
				unrelated := subject.Namespace == "other" || subject.Name == "unrelated" || subject.Name == "system:serviceaccounts:other"
				if (scenario.allowed || unrelated) && err != nil || !scenario.allowed && !unrelated && err == nil {
					test.Fatalf("unexpected result: %v", err)
				}
				if scenario.missingRole && !unrelated && !apierrors.IsNotFound(err) {
					test.Fatalf("missing role was not surfaced: %v", err)
				}
			})
		}
	}
}

func TestVerifyDrainedReadFailures(test *testing.T) {
	for _, target := range []string{"controller", "shared-role", "*v1.RoleBindingList", "*v1.ClusterRoleBindingList", "*v1.PodList", "DeploymentList", "ReplicaSetList", "StatefulSetList", "DaemonSetList", "JobList", "CronJobList", "ReplicationControllerList"} {
		for _, failure := range []error{context.Canceled, context.DeadlineExceeded, apierrors.NewForbidden(schema.GroupResource{Resource: "objects"}, "denied", errors.New("denied"))} {
			test.Run(target+"/"+failure.Error(), func(test *testing.T) {
				manifest, admin := fixture(test,
					&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:discovery"}},
					&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "shared"}, Subjects: []rbacv1.Subject{{Kind: "Group", Name: "system:authenticated"}}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:discovery"}},
				)
				wrapped := interceptor.NewClient(admin.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						if target == "controller" && key.Name == "controller" || target == "shared-role" && key.Name == "system:discovery" {
							return failure
						}
						return delegate.Get(ctx, key, object, opts...)
					},
					List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if fmt.Sprintf("%T", list) == target || list.GetObjectKind().GroupVersionKind().Kind == target {
							return failure
						}
						return delegate.List(ctx, list, opts...)
					},
				})
				if err := VerifyDrained(context.Background(), wrapped, manifest); !errors.Is(err, failure) {
					test.Fatalf("lost API failure: %v", err)
				}
			})
		}
	}
}

func TestVerifyDrainedProducerIdentity(test *testing.T) {
	for _, kind := range []string{"Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob", "ReplicationController"} {
		for _, field := range []string{"serviceAccountName", "serviceAccount"} {
			for _, account := range []string{"controller", "unrelated", "default", ""} {
				test.Run(kind+"/"+field+"/"+account, func(test *testing.T) {
					manifest, admin := fixture(test)
					producer := unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "apps/v1", "kind": kind, "metadata": map[string]interface{}{"name": "legacy", "namespace": "control"}}}
					if kind == "Job" || kind == "CronJob" {
						producer.SetAPIVersion("batch/v1")
					}
					if kind == "ReplicationController" {
						producer.SetAPIVersion("v1")
					}
					path := []string{"spec", "template", "spec", field}
					if kind == "CronJob" {
						path = []string{"spec", "jobTemplate", "spec", "template", "spec", field}
					}
					if err := unstructured.SetNestedField(producer.Object, account, path...); err != nil {
						test.Fatal(err)
					}
					wrapped := interceptor.NewClient(admin.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if list.GetObjectKind().GroupVersionKind().Kind == kind+"List" {
							options := (&client.ListOptions{}).ApplyOptions(opts)
							if options.Namespace == "control" {
								if err := delegate.List(ctx, list, opts...); err != nil {
									return err
								}
								objects := list.(*unstructured.UnstructuredList)
								objects.Items = append(objects.Items, producer)
								return nil
							}
						}
						return delegate.List(ctx, list, opts...)
					}})
					err := VerifyDrained(context.Background(), wrapped, manifest)
					if account == "controller" {
						if err == nil || !strings.Contains(err.Error(), "producer "+kind+" control/legacy") {
							test.Fatalf("expected producer rejection, got %v", err)
						}
					} else if err != nil {
						test.Fatal(err)
					}
				})
			}
		}
	}
}

func TestVerifyDrainedStoppedReplicaSet(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		mutate  func(*unstructured.Unstructured, *appsv1.Deployment)
		allowed bool
	}{
		{name: "stopped-owned", allowed: true},
		{name: "wrong-uid", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			deployment.UID = "replacement"
		}},
		{name: "stale-generation", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) { producer.SetGeneration(4) }},
		{name: "omitted-replicas", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			unstructured.RemoveNestedField(producer.Object, "spec", "replicas")
		}},
		{name: "active", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			producer.Object["spec"].(map[string]interface{})["replicas"] = int64(1)
		}},
		{name: "observed-active", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			producer.Object["status"].(map[string]interface{})["replicas"] = int64(1)
		}},
		{name: "malformed-current", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			producer.Object["status"].(map[string]interface{})["replicas"] = "zero"
		}},
		{name: "missing-observed", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			unstructured.RemoveNestedField(producer.Object, "status", "observedGeneration")
		}},
		{name: "missing-current", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			unstructured.RemoveNestedField(producer.Object, "status", "replicas")
		}},
		{name: "future-observed", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			producer.Object["status"].(map[string]interface{})["observedGeneration"] = int64(4)
		}},
		{name: "ready", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			producer.Object["status"].(map[string]interface{})["readyReplicas"] = int64(1)
		}},
		{name: "available", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			producer.Object["status"].(map[string]interface{})["availableReplicas"] = int64(1)
		}},
		{name: "labeled", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			producer.Object["status"].(map[string]interface{})["fullyLabeledReplicas"] = int64(1)
		}},
		{name: "terminating", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			producer.Object["status"].(map[string]interface{})["terminatingReplicas"] = int64(1)
		}},
		{name: "malformed-ready", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			producer.Object["status"].(map[string]interface{})["readyReplicas"] = "zero"
		}},
		{name: "missing-controller-uid", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) { deployment.UID = "" }},
		{name: "wrong-owner-version", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			owners := producer.GetOwnerReferences()
			owners[0].APIVersion = "apps/v1beta1"
			producer.SetOwnerReferences(owners)
		}},
		{name: "wrong-owner-kind", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			owners := producer.GetOwnerReferences()
			owners[0].Kind = "StatefulSet"
			producer.SetOwnerReferences(owners)
		}},
		{name: "non-controller-owner", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			owners := producer.GetOwnerReferences()
			owners[0].Controller = ptr.To(false)
			producer.SetOwnerReferences(owners)
		}},
		{name: "deleting-replicaset", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			now := metav1.Now()
			producer.SetDeletionTimestamp(&now)
		}},
		{name: "deleting-controller", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			now := metav1.Now()
			deployment.DeletionTimestamp = &now
		}},
		{name: "foreign-owner", mutate: func(producer *unstructured.Unstructured, deployment *appsv1.Deployment) {
			owners := producer.GetOwnerReferences()
			owners[0].Name = "foreign"
			producer.SetOwnerReferences(owners)
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			manifest, admin := fixture(test)
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "control", UID: "current", Generation: 3}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(0))}, Status: appsv1.DeploymentStatus{ObservedGeneration: 3}}
			producer := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": map[string]interface{}{"name": "current-rs", "namespace": "control", "generation": int64(3)}, "spec": map[string]interface{}{"replicas": int64(0), "template": map[string]interface{}{"spec": map[string]interface{}{"serviceAccountName": "controller"}}}, "status": map[string]interface{}{"replicas": int64(0), "observedGeneration": int64(3)}}}
			producer.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "controller", UID: "current", Controller: ptr.To(true)}})
			if scenario.mutate != nil {
				scenario.mutate(producer, deployment)
			}
			wrapped := interceptor.NewClient(admin.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if key.Name == "controller" {
						deployment.DeepCopyInto(object.(*appsv1.Deployment))
						return nil
					}
					return delegate.Get(ctx, key, object, opts...)
				},
				List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if list.GetObjectKind().GroupVersionKind().Kind == "DeploymentList" && (&client.ListOptions{}).ApplyOptions(opts).Namespace == "control" {
						object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(deployment)
						if err != nil {
							return err
						}
						list.(*unstructured.UnstructuredList).Items = []unstructured.Unstructured{{Object: object}}
						return nil
					}
					if list.GetObjectKind().GroupVersionKind().Kind == "ReplicaSetList" && (&client.ListOptions{}).ApplyOptions(opts).Namespace == "control" {
						list.(*unstructured.UnstructuredList).Items = []unstructured.Unstructured{*producer}
						return nil
					}
					return delegate.List(ctx, list, opts...)
				},
			})
			err := VerifyDrained(context.Background(), wrapped, manifest)
			if scenario.allowed && err != nil || !scenario.allowed && err == nil {
				test.Fatalf("allowed=%t, got %v", scenario.allowed, err)
			}
		})
	}
}

func TestVerifyDrainedFreshAndTerminatingPods(test *testing.T) {
	manifest, admin := fixture(test)
	if err := admin.Delete(context.Background(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "control"}}); err != nil {
		test.Fatal(err)
	}
	if err := VerifyDrained(context.Background(), admin, manifest); err != nil {
		test.Fatal(err)
	}
	for _, namespace := range []string{"control", "tenant"} {
		test.Run(namespace, func(test *testing.T) {
			now := metav1.Now()
			manifest, admin := fixture(test, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "terminating", Namespace: namespace, DeletionTimestamp: &now, Finalizers: []string{"test.example/hold"}}, Spec: corev1.PodSpec{DeprecatedServiceAccount: "controller"}})
			if err := VerifyDrained(context.Background(), admin, manifest); err == nil {
				test.Fatal("accepted terminating controller/workload Pod")
			}
		})
	}
}

func TestVerifyDrainedControllerSnapshot(test *testing.T) {
	for _, scenario := range []string{"replaced", "restarted", "disappeared"} {
		test.Run(scenario, func(test *testing.T) {
			manifest, admin := fixture(test)
			wrapped := interceptor.NewClient(admin.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := delegate.List(ctx, list, opts...); err != nil {
					return err
				}
				if list.GetObjectKind().GroupVersionKind().Kind == "DeploymentList" && (&client.ListOptions{}).ApplyOptions(opts).Namespace == "control" {
					objects := list.(*unstructured.UnstructuredList)
					for index := range objects.Items {
						if objects.Items[index].GetName() != "controller" {
							continue
						}
						switch scenario {
						case "replaced":
							objects.Items[index].SetUID("replacement")
						case "restarted":
							objects.Items[index].SetGeneration(2)
						case "disappeared":
							objects.Items = append(objects.Items[:index], objects.Items[index+1:]...)
						}
						break
					}
				}
				return nil
			}})
			if err := VerifyDrained(context.Background(), wrapped, manifest); err == nil || !strings.Contains(err.Error(), "during drain verification") {
				test.Fatalf("accepted changing controller: %v", err)
			}
		})
	}
}

func TestVerifyDrainedControllerShutdown(test *testing.T) {
	for name, mutate := range map[string]func(*appsv1.Deployment){
		"omitted-desired": func(deployment *appsv1.Deployment) { deployment.Spec.Replicas = nil },
		"active-desired":  func(deployment *appsv1.Deployment) { deployment.Spec.Replicas = ptr.To(int32(1)) },
		"current":         func(deployment *appsv1.Deployment) { deployment.Status.Replicas = 1 },
		"updated":         func(deployment *appsv1.Deployment) { deployment.Status.UpdatedReplicas = 1 },
		"ready":           func(deployment *appsv1.Deployment) { deployment.Status.ReadyReplicas = 1 },
		"available":       func(deployment *appsv1.Deployment) { deployment.Status.AvailableReplicas = 1 },
		"stale": func(deployment *appsv1.Deployment) {
			deployment.Generation = 2
			deployment.Status.ObservedGeneration = 1
		},
		"future": func(deployment *appsv1.Deployment) {
			deployment.Generation = 1
			deployment.Status.ObservedGeneration = 2
		},
	} {
		test.Run(name, func(test *testing.T) {
			manifest, admin := fixture(test)
			wrapped := interceptor.NewClient(admin.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
				if err := delegate.Get(ctx, key, object, opts...); err != nil {
					return err
				}
				if key.Name == "controller" {
					mutate(object.(*appsv1.Deployment))
				}
				return nil
			}})
			if err := VerifyDrained(context.Background(), wrapped, manifest); err == nil || !strings.Contains(err.Error(), "has not completed shutdown") {
				test.Fatalf("accepted incomplete shutdown: %v", err)
			}
		})
	}
}

func TestVerifyDrainedInvalidInputs(test *testing.T) {
	for _, mutate := range []func(*Manifest){
		func(manifest *Manifest) { manifest.Namespace = "control/other" },
		func(manifest *Manifest) { manifest.Controller = "default" },
		func(manifest *Manifest) { manifest.Controller = " controller" },
		func(manifest *Manifest) { manifest.Deployment = "../controller" },
		func(manifest *Manifest) { manifest.Namespaces = []string{"tenant", "tenant"} },
		func(manifest *Manifest) { manifest.Namespaces = []string{"tenant", ""} },
		func(manifest *Manifest) { manifest.Namespaces = []string{" tenant"} },
		func(manifest *Manifest) { manifest.Namespaces = []string{"control"} },
	} {
		manifest, _ := fixture(test)
		mutate(&manifest)
		if err := VerifyDrained(context.Background(), nil, manifest); err == nil {
			test.Fatal("accepted invalid input")
		}
	}
}
