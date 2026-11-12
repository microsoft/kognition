// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"errors"
	"fmt"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func cleanupProbes(ctx context.Context, admin client.Client, created []client.Object) error {
	var grants, fixtures, protections []client.Object
	for index := len(created) - 1; index >= 0; index-- {
		object := created[index]
		switch object.(type) {
		case *rbacv1.RoleBinding, *rbacv1.Role:
			grants = append(grants, object)
		case *admissionv1.ValidatingAdmissionPolicyBinding, *admissionv1.ValidatingAdmissionPolicy:
			protections = append(protections, object)
		default:
			fixtures = append(fixtures, object)
		}
	}
	result := deleteProbes(ctx, admin, grants)
	result = errors.Join(result, deleteProbes(ctx, admin, fixtures))
	if result != nil {
		for _, object := range protections {
			result = errors.Join(result, fmt.Errorf("retained dry-run protection %T %s UID=%s; resolve probe cleanup before removal", object, object.GetName(), object.GetUID()))
		}
		return result
	}
	return deleteProbes(ctx, admin, protections)
}

func deleteProbes(ctx context.Context, admin client.Client, objects []client.Object) error {
	var result error
	var pending []client.Object
	for _, object := range objects {
		uid := object.GetUID()
		if uid == "" {
			result = errors.Join(result, fmt.Errorf("missing cleanup UID for %T %s/%s", object, object.GetNamespace(), object.GetName()))
			continue
		}
		request, cancel := context.WithTimeout(ctx, 5*time.Second)
		preconditions := client.Preconditions{UID: &uid}
		if version := object.GetResourceVersion(); version != "" && object.GetAnnotations()[transactionOwner] != "" {
			preconditions.ResourceVersion = &version
		}
		err := admin.Delete(request, object, preconditions, client.PropagationPolicy(metav1.DeletePropagationBackground))
		cancel()
		if err != nil && !apierrors.IsNotFound(err) {
			result = errors.Join(result, fmt.Errorf("cleanup %T %s/%s UID=%s: %w", object, object.GetNamespace(), object.GetName(), uid, err))
		} else {
			pending = append(pending, object)
		}
	}
	if len(pending) == 0 {
		return result
	}
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		remaining := pending[:0]
		for _, object := range pending {
			current, ok := object.DeepCopyObject().(client.Object)
			if !ok {
				return false, fmt.Errorf("probe copy does not implement client.Object")
			}
			err := admin.Get(ctx, client.ObjectKeyFromObject(object), current)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err == nil && current.GetUID() != object.GetUID() {
				result = errors.Join(result, fmt.Errorf("probe replaced during cleanup: %T %s/%s", object, object.GetNamespace(), object.GetName()))
				continue
			}
			if err != nil {
				result = errors.Join(result, err)
			}
			remaining = append(remaining, object)
		}
		pending = remaining
		return len(pending) == 0, nil
	})
	if err != nil {
		for _, object := range pending {
			result = errors.Join(result, fmt.Errorf("confirm cleanup %T %s/%s UID=%s: %w", object, object.GetNamespace(), object.GetName(), object.GetUID(), err))
		}
	}
	return result
}
