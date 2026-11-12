// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func createAdmissionFixture(ctx context.Context, admin client.Client, object client.Object, created *[]client.Object) error {
	const marker = "kognition.dev/admission-fixture-owner"
	annotations := maps.Clone(object.GetAnnotations())
	if annotations == nil {
		annotations = map[string]string{}
	}
	owner := annotations[marker]
	if owner == "" {
		owner = uuid.NewString()
	}
	annotations[marker] = owner
	object.SetAnnotations(annotations)
	err := admin.Create(ctx, object)
	if err == nil {
		*created = append(*created, object.DeepCopyObject().(client.Object))
		return nil
	}
	if apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("existing fixture left untouched: %w", err)
	}
	recovery, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	current := object.DeepCopyObject().(client.Object)
	readErr := admin.Get(recovery, client.ObjectKeyFromObject(object), current)
	if readErr == nil && current.GetUID() != "" && current.GetAnnotations()[marker] == owner {
		*created = append(*created, current)
		return fmt.Errorf("fixture Create response failed; recovered owned UID=%s for cleanup: %w", current.GetUID(), err)
	}
	if readErr != nil {
		return fmt.Errorf("fixture %T %s/%s owner=%s unresolved (GET: %w); inspect exact marker for late commits: %w", object, object.GetNamespace(), object.GetName(), owner, readErr, err)
	}
	return fmt.Errorf("fixture %T %s/%s owner=%s unresolved (GET found no matching owned UID); inspect exact marker for late commits: %w", object, object.GetNamespace(), object.GetName(), owner, err)
}

func TestAdmissionFixtureOwnership(test *testing.T) {
	for _, scenario := range []string{"success", "lost-response", "collision", "foreign-owner", "missing"} {
		test.Run(scenario, func(test *testing.T) {
			object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "tenant", Annotations: map[string]string{"existing": "preserved", "kognition.dev/admission-fixture-owner": "sealed-owner"}}}
			failure := errors.New("lost Create response")
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, underlying client.WithWatch, target client.Object, options ...client.CreateOption) error {
					if scenario == "collision" {
						return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, target.GetName())
					}
					if scenario != "missing" {
						stored := target.DeepCopyObject().(client.Object)
						stored.SetUID("server-uid")
						if scenario == "foreign-owner" {
							stored.SetAnnotations(nil)
						}
						if err := underlying.Create(ctx, stored, options...); err != nil {
							return err
						}
					}
					if scenario == "success" {
						target.SetUID("server-uid")
						return nil
					}
					return failure
				},
			}).Build()
			var tracked []client.Object
			err := createAdmissionFixture(context.Background(), admin, object, &tracked)
			if object.Annotations["existing"] != "preserved" || object.Annotations["kognition.dev/admission-fixture-owner"] != "sealed-owner" {
				test.Fatal("expected metadata changed")
			}
			if scenario == "success" || scenario == "lost-response" {
				if len(tracked) != 1 || tracked[0].GetUID() != "server-uid" {
					test.Fatalf("owned fixture untracked: %v %+v", err, tracked)
				}
			} else if len(tracked) != 0 {
				test.Fatal("unowned object adopted")
			}
			if scenario == "success" {
				if err != nil {
					test.Fatal(err)
				}
				return
			}
			if scenario == "collision" {
				if !apierrors.IsAlreadyExists(err) {
					test.Fatal(err)
				}
				return
			}
			if !errors.Is(err, failure) {
				test.Fatalf("failure lost: %v", err)
			}
			if scenario == "missing" {
				var statusError *apierrors.StatusError
				if !errors.As(err, &statusError) || !apierrors.IsNotFound(statusError) {
					test.Fatalf("recovery GET failure lost: %v", err)
				}
			}
		})
	}
}
