// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func deployedPolicyLifecycle(test *testing.T, ctx context.Context, admin client.Client, manifest activation.Manifest, invocation *networkingv1.NetworkPolicy, installationObjects, invocationObjects *[]client.Object, deny func(string), network func(string)) {
	test.Helper()
	baseline := &networkingv1.NetworkPolicy{}
	for _, object := range manifest.Objects {
		if object.GetKind() == "NetworkPolicy" && object.GetNamespace() == invocation.Namespace && strings.HasSuffix(object.GetName(), "-stdio-baseline") {
			if err := admin.Get(ctx, client.ObjectKeyFromObject(object), baseline); err != nil {
				test.Fatal(err)
			}
		}
	}
	if baseline.UID == "" {
		test.Fatal("missing tracked baseline")
	}
	for _, scenario := range []struct {
		name, deleting, missing string
		policy                  *networkingv1.NetworkPolicy
		tracked                 *[]client.Object
	}{
		{"baseline", "independent deny-egress baseline", "cannot verify attachment isolation baseline", baseline, installationObjects},
		{"invocation", "current Job-owned deny-egress NetworkPolicy", "cannot verify attachment NetworkPolicy", invocation, invocationObjects},
	} {
		func() {
			original := scenario.policy.DeepCopy()
			trackedIndex := -1
			for index, object := range *scenario.tracked {
				if object.GetUID() == original.UID {
					trackedIndex = index
				}
			}
			if trackedIndex < 0 {
				test.Fatal("policy not tracked for cleanup")
			}
			restored := false
			defer func() {
				if restored {
					return
				}
				recovery, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				current := &networkingv1.NetworkPolicy{}
				err := admin.Get(recovery, client.ObjectKeyFromObject(original), current)
				if err == nil && current.UID == scenario.policy.UID {
					current.Finalizers = nil
					current.Spec = original.Spec
					if err = admin.Update(recovery, current); err != nil {
						test.Errorf("policy recovery: %v", err)
						return
					}
					if current.DeletionTimestamp == nil {
						(*scenario.tracked)[trackedIndex] = current.DeepCopy()
						return
					}
					err = wait.PollUntilContextTimeout(recovery, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
						readErr := admin.Get(ctx, client.ObjectKeyFromObject(original), current)
						return apierrors.IsNotFound(readErr), client.IgnoreNotFound(readErr)
					})
					if err == nil {
						err = admin.Get(recovery, client.ObjectKeyFromObject(original), current)
					}
				}
				if !apierrors.IsNotFound(err) {
					test.Errorf("policy recovery requires absence or owned UID: %v", err)
					return
				}
				replacement := original.DeepCopy()
				replacement.ResourceVersion, replacement.UID = "", ""
				replacement.ManagedFields = nil
				if err := admin.Create(recovery, replacement); err != nil {
					test.Errorf("restore policy: %v", err)
					return
				}
				(*scenario.tracked)[trackedIndex] = replacement.DeepCopy()
			}()
			scenario.policy.Finalizers = []string{"kognition.dev/sg07-cleanup-test"}
			if err := admin.Update(ctx, scenario.policy); err != nil {
				test.Fatal(err)
			}
			if err := admin.Delete(ctx, scenario.policy, client.Preconditions{UID: ptr.To(original.UID)}); err != nil {
				test.Fatal(err)
			}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(original), scenario.policy); err != nil || scenario.policy.DeletionTimestamp == nil {
				test.Fatalf("expected deleting policy: %v", err)
			}
			deny(scenario.deleting)
			network(scenario.name + "-deleting")
			scenario.policy.Finalizers = nil
			if err := admin.Update(ctx, scenario.policy); err != nil {
				test.Fatal(err)
			}
			if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
				err := admin.Get(ctx, client.ObjectKeyFromObject(original), &networkingv1.NetworkPolicy{})
				return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
			}); err != nil {
				test.Fatal(err)
			}
			deny(scenario.missing)
			network(scenario.name + "-absent")
			replacement := original.DeepCopy()
			replacement.ResourceVersion, replacement.UID = "", ""
			replacement.ManagedFields = nil
			if scenario.name == "invocation" {
				replacement.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
			}
			if err := admin.Create(ctx, replacement); err != nil {
				test.Fatal(err)
			}
			*scenario.policy = *replacement.DeepCopy()
			(*scenario.tracked)[trackedIndex] = replacement.DeepCopy()
			if err := admin.Delete(ctx, original, client.Preconditions{UID: ptr.To(original.UID)}); !apierrors.IsConflict(err) {
				test.Fatalf("stale cleanup must fail without deleting replacement: %v", err)
			}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(replacement), scenario.policy); err != nil || scenario.policy.UID != replacement.UID {
				test.Fatalf("replacement changed after rejected cleanup: %v", err)
			}
			if scenario.name == "invocation" {
				deny(scenario.deleting)
				network("invocation-replaced-with-allow")
				scenario.policy.Spec = original.Spec
				if err := admin.Update(ctx, scenario.policy); err != nil {
					test.Fatal(err)
				}
			}
			restored = true
			test.Logf("%s deletion/absence/replacement and stale-UID cleanup rejection confirmed: oldUID=%s replacementUID=%s", scenario.name, original.UID, replacement.UID)
		}()
	}
}
