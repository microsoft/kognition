// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestProbeCleanup(t *testing.T) {
	for _, scenario := range []string{"success", "grant-denied", "fixture-denied", "stuck", "replacement", "missing-uid", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			protection := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "protection", UID: "protection-uid"}}
			binding := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: "protection", UID: "binding-uid"}}
			first := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "tenant", UID: "first-uid"}}
			second := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "tenant", UID: "second-uid"}}
			grant := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "grant", Namespace: "tenant", UID: "grant-uid"}}
			objects := []client.Object{protection, binding, first, second, grant}
			stored := make([]client.Object, len(objects))
			for index, object := range objects {
				stored[index] = object.DeepCopyObject().(client.Object)
			}
			if scenario == "replacement" {
				stored[2].SetUID("replacement-uid")
			}
			if scenario == "missing-uid" {
				first.UID = ""
			}
			if scenario == "stuck" {
				stored[3].SetFinalizers([]string{"test.kognition.dev/retain"})
			}
			var deleted []string
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(stored...).WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.DeleteOption) error {
					deleted = append(deleted, object.GetName())
					settings := (&client.DeleteOptions{}).ApplyOptions(options)
					if settings.Preconditions == nil || settings.Preconditions.UID == nil || *settings.Preconditions.UID != object.GetUID() || settings.PropagationPolicy == nil || *settings.PropagationPolicy != metav1.DeletePropagationBackground {
						t.Fatal("cleanup omitted UID or background propagation")
					}
					if scenario == "grant-denied" && object.GetName() == "grant" || scenario == "fixture-denied" && object.GetName() == "first" {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "probe"}, object.GetName(), errors.New("fixture denial"))
					}
					if scenario == "cancelled" {
						return ctx.Err()
					}
					current := object.DeepCopyObject().(client.Object)
					if err := underlying.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
						return err
					}
					if current.GetUID() != *settings.Preconditions.UID {
						return apierrors.NewConflict(schema.GroupResource{Resource: "probe"}, object.GetName(), errors.New("UID precondition failed"))
					}
					return underlying.Delete(ctx, object, options...)
				},
			}).Build()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			err := cleanupProbes(ctx, admin, objects)
			if scenario == "success" {
				if err != nil {
					t.Fatal(err)
				}
				for _, object := range objects {
					if err := admin.Get(context.Background(), client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
						t.Fatalf("probe retained: %T: %v", object, err)
					}
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "retained dry-run protection") {
					t.Fatalf("expected failure with retained protection: %v", err)
				}
				if err := admin.Get(context.Background(), client.ObjectKeyFromObject(protection), protection); err != nil {
					t.Fatal("cleanup removed safety policy after failure")
				}
				if err := admin.Get(context.Background(), client.ObjectKeyFromObject(binding), binding); err != nil {
					t.Fatal("cleanup removed safety binding after failure")
				}
			}
			if len(deleted) == 0 || deleted[0] != "grant" {
				t.Fatalf("grants were not revoked first: %v", deleted)
			}
			if scenario != "cancelled" && scenario != "stuck" {
				if err := admin.Get(context.Background(), client.ObjectKeyFromObject(second), second); !apierrors.IsNotFound(err) {
					t.Fatalf("unrelated cleanup was skipped: %v", err)
				}
			}
			if scenario == "stuck" {
				if err := admin.Get(context.Background(), client.ObjectKeyFromObject(first), first); !apierrors.IsNotFound(err) {
					t.Fatal("stuck object prevented later deletion")
				}
			}
			if scenario == "replacement" {
				if err := admin.Get(context.Background(), client.ObjectKeyFromObject(first), first); err != nil || first.UID != "replacement-uid" {
					t.Fatal("replacement was removed")
				}
			}
		})
	}
}

func TestPolicyBehaviorRequiresExplicitConfig(t *testing.T) {
	if _, err := VerifyPolicyBehavior(context.Background(), nil, Manifest{}); err == nil {
		t.Fatal("accepted absent Kubernetes configuration")
	}
}
