// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

type ResourceIntent struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	Probe      bool   `json:"probe"`
	Rejected   bool   `json:"rejected,omitempty"`
}

func saveResourceIntents(ctx context.Context, admin client.Client, journal *corev1.ConfigMap, resources []ResourceIntent) error {
	var audit Audit
	if err := decodeStrict([]byte(journal.Data["audit.json"]), &audit); err != nil {
		return err
	}
	audit.Resources = resources
	encoded, err := json.Marshal(audit)
	if err != nil {
		return err
	}
	updated := journal.DeepCopy()
	updated.Data["audit.json"] = string(encoded)
	if err := admin.Update(ctx, updated); err != nil {
		return fmt.Errorf("persist resource intent: %w", err)
	}
	*journal = *updated
	return nil
}

func createJournaledResource(ctx context.Context, admin client.Client, journal *corev1.ConfigMap, object client.Object, probe bool) error {
	if journal.UID == "" || object.GetName() == "" {
		return errors.New("durable resource creation requires journal UID and exact name")
	}
	gvk, err := apiutil.GVKForObject(object, scheme.Scheme)
	if err != nil {
		return err
	}
	var audit Audit
	if err := decodeStrict([]byte(journal.Data["audit.json"]), &audit); err != nil {
		return err
	}
	intent := ResourceIntent{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind, Namespace: object.GetNamespace(), Name: object.GetName(), Probe: probe}
	for _, existing := range audit.Resources {
		if existing.APIVersion == intent.APIVersion && existing.Kind == intent.Kind && existing.Namespace == intent.Namespace && existing.Name == intent.Name {
			return errors.New("resource Create must never be retried within a transaction")
		}
	}
	audit.Resources = append(audit.Resources, intent)
	if err := saveResourceIntents(ctx, admin, journal, audit.Resources); err != nil {
		return err
	}
	annotations := maps.Clone(object.GetAnnotations())
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[transactionOwner] = string(journal.UID)
	object.SetAnnotations(annotations)
	createErr := admin.Create(ctx, object)
	intent.UID = string(object.GetUID())
	if createErr != nil {
		intent.UID = ""
		intent.Rejected = apierrors.IsAlreadyExists(createErr) || apierrors.IsForbidden(createErr) || apierrors.IsInvalid(createErr) || apierrors.IsUnauthorized(createErr) || apierrors.IsBadRequest(createErr)
		if !intent.Rejected {
			return fmt.Errorf("unresolved Create %s %s/%s transaction=%s: %w", intent.Kind, intent.Namespace, intent.Name, journal.UID, createErr)
		}
	} else if intent.UID == "" {
		return errors.New("created resource has no UID; Create remains unresolved")
	}
	audit.Resources[len(audit.Resources)-1] = intent
	return errors.Join(createErr, saveResourceIntents(ctx, admin, journal, audit.Resources))
}

func cleanupJournaledResources(ctx context.Context, admin client.Client, journal *corev1.ConfigMap, probes bool) error {
	var audit Audit
	if err := decodeStrict([]byte(journal.Data["audit.json"]), &audit); err != nil {
		return err
	}
	var objects []client.Object
	var result error
	for index, intent := range audit.Resources {
		if intent.Probe != probes || intent.Rejected {
			continue
		}
		gvk := schema.FromAPIVersionAndKind(intent.APIVersion, intent.Kind)
		instance, err := scheme.Scheme.New(gvk)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		object, ok := instance.(client.Object)
		if !ok {
			result = errors.Join(result, errors.New("journal resource is not a Kubernetes object"))
			continue
		}
		key := client.ObjectKey{Namespace: intent.Namespace, Name: intent.Name}
		err = admin.Get(ctx, key, object)
		if apierrors.IsNotFound(err) && intent.UID != "" {
			continue
		}
		if err != nil {
			result = errors.Join(result, fmt.Errorf("unresolved Create or cleanup %s %s transaction=%s UID=%s: %w", intent.Kind, key, journal.UID, intent.UID, err))
			continue
		}
		if object.GetUID() == "" || object.GetAnnotations()[transactionOwner] != string(journal.UID) || len(object.GetOwnerReferences()) != 0 || (intent.UID != "" && object.GetUID() != types.UID(intent.UID)) {
			result = errors.Join(result, fmt.Errorf("refusing to delete foreign or replaced journal resource %s %s", intent.Kind, key))
			continue
		}
		if intent.UID == "" {
			audit.Resources[index].UID = string(object.GetUID())
			if err := saveResourceIntents(ctx, admin, journal, audit.Resources); err != nil {
				return errors.Join(result, err)
			}
		}
		objects = append(objects, object)
	}
	if result != nil {
		var cleanable []client.Object
		for _, object := range objects {
			switch object.(type) {
			case *admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding:
				result = errors.Join(result, fmt.Errorf("retained dry-run protection %s UID=%s", object.GetName(), object.GetUID()))
			default:
				cleanable = append(cleanable, object)
			}
		}
		objects = cleanable
	}
	return errors.Join(result, cleanupProbes(ctx, admin, objects))
}
