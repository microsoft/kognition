// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const probeOwnershipAnnotation = "kognition.dev/admission-probe-owner"

func createProbe(ctx context.Context, admin client.Client, object client.Object, created *[]client.Object) error {
	annotations := maps.Clone(object.GetAnnotations())
	if annotations == nil {
		annotations = map[string]string{}
	}
	owner := uuid.NewString()
	annotations[probeOwnershipAnnotation] = owner
	object.SetAnnotations(annotations)
	err := admin.Create(ctx, object)
	if err == nil {
		createdObject, ok := object.DeepCopyObject().(client.Object)
		if !ok {
			*created = append(*created, object)
			return fmt.Errorf("created probe copy does not implement client.Object")
		}
		*created = append(*created, createdObject)
		return nil
	}
	if apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("probe name collision %T %s/%s; existing object untouched: %w", object, object.GetNamespace(), object.GetName(), err)
	}
	recovery, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	current, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return errors.Join(err, fmt.Errorf("probe recovery copy does not implement client.Object"))
	}
	readErr := admin.Get(recovery, client.ObjectKeyFromObject(object), current)
	if readErr == nil && current.GetUID() != "" && current.GetAnnotations()[probeOwnershipAnnotation] == owner {
		*created = append(*created, current)
		return fmt.Errorf("create probe %T %s/%s failed; recovered owned UID=%s for cleanup: %w", object, object.GetNamespace(), object.GetName(), current.GetUID(), err)
	}
	unresolved, ok := object.DeepCopyObject().(client.Object)
	if !ok {
		return errors.Join(err, readErr, fmt.Errorf("unresolved probe copy does not implement client.Object"))
	}
	unresolved.SetUID("")
	*created = append(*created, unresolved)
	return fmt.Errorf("create probe %T %s/%s owner=%s unresolved; inspect this exact ownership marker before cleanup: %w", object, object.GetNamespace(), object.GetName(), owner, errors.Join(err, readErr))
}
