// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPolicyProbeAttribution(test *testing.T) {
	for _, scenario := range []string{"exact", "rbac-only", "wrong-policy", "wrong-binding", "positive-denied", "replacement", "no-enforcement", "conflict"} {
		test.Run(scenario, func(test *testing.T) {
			object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "tenant", UID: "owned", Labels: map[string]string{"managed": "true"}}}
			stored := object.DeepCopy()
			if scenario == "replacement" {
				stored.UID = "replacement"
			}
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(stored).Build()
			calls := 0
			controller := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
				Update: func(ctx context.Context, underlying client.WithWatch, target client.Object, options ...client.UpdateOption) error {
					calls++
					if selected := (&client.UpdateOptions{}).ApplyOptions(options); len(selected.DryRun) != 1 || selected.DryRun[0] != metav1.DryRunAll {
						test.Fatal("persistent probe update")
					}
					resource := schema.GroupResource{Resource: "configmaps"}
					if scenario == "conflict" && calls == 1 {
						return apierrors.NewConflict(resource, target.GetName(), errors.New("resource version"))
					}
					if scenario == "positive-denied" {
						return apierrors.NewForbidden(resource, target.GetName(), errors.New("permission denied"))
					}
					if len(target.GetLabels()) != 0 || scenario == "no-enforcement" {
						return nil
					}
					policy, binding := "expected", "expected"
					if scenario == "wrong-policy" {
						policy = "other"
					}
					if scenario == "wrong-binding" {
						binding = "other"
					}
					reason := fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request", policy, binding)
					if scenario == "rbac-only" {
						reason = "cannot update configmaps"
					}
					return apierrors.NewForbidden(resource, target.GetName(), errors.New(reason))
				},
			}).Build()
			timeout := time.Second
			if scenario == "no-enforcement" {
				timeout = 40 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			result, err := runPolicyProbe(ctx, admin, controller, policyProbe{policy: "expected", object: object})
			if scenario == "exact" || scenario == "conflict" {
				if err != nil || !result.Allowed || !result.Denied || result.UID != object.UID {
					test.Fatalf("result=%+v error=%v", result, err)
				}
				return
			}
			if err == nil || result.Denied {
				test.Fatalf("false acceptance: %+v error=%v", result, err)
			}
			if scenario == "positive-denied" && (result.Allowed || calls != 1 || !strings.Contains(err.Error(), "positive probe")) {
				test.Fatalf("positive denial retried or accepted: %+v calls=%d error=%v", result, calls, err)
			}
			if scenario == "replacement" && calls != 0 {
				test.Fatal("replacement object updated")
			}
		})
	}
}
