// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestProbeCreationOwnership(t *testing.T) {
	for _, scenario := range []string{"success", "lost-response", "collision", "foreign-owner", "missing", "get-denied"} {
		t.Run(scenario, func(t *testing.T) {
			object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "tenant"}}
			failure := errors.New("lost create response")
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
				Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, target client.Object, options ...client.GetOption) error {
					if scenario == "get-denied" {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, errors.New("denied"))
					}
					return underlying.Get(ctx, key, target, options...)
				},
			}).Build()
			var tracked []client.Object
			err := createProbe(context.Background(), admin, object, &tracked)
			if scenario == "collision" {
				if !apierrors.IsAlreadyExists(err) || len(tracked) != 0 {
					t.Fatalf("collision adopted: %v %+v", err, tracked)
				}
				return
			}
			if len(tracked) != 1 {
				t.Fatalf("untracked create: %v %+v", err, tracked)
			}
			if scenario == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("original failure lost: %v", err)
			}
			if scenario == "success" || scenario == "lost-response" {
				if tracked[0].GetUID() != "server-uid" {
					t.Fatal("owned UID not recovered")
				}
			} else if tracked[0].GetUID() != "" {
				t.Fatal("unresolved object adopted")
			}
			if tracked[0].GetAnnotations()[probeOwnershipAnnotation] == "" {
				t.Fatal("ownership marker absent")
			}
		})
	}
}
