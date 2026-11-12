// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package convert provides bidirectional conversion between the internal
// kognition API types and the Kubernetes CRD types.
package convert

import (
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// Ensure imports are used.
var (
	_ = k8s.SchemeBuilder
)

// --- ObjectMeta ---

func objectMetaToK8s(in api.ObjectMeta) metav1.ObjectMeta {
	out := metav1.ObjectMeta{
		Name:        in.Name,
		Namespace:   in.Namespace,
		UID:         types.UID(in.UID),
		Labels:      in.Labels,
		Annotations: in.Annotations,
		Finalizers:  in.Finalizers,
		Generation:  in.Generation,
	}
	if in.ResourceVersion != 0 {
		out.ResourceVersion = strconv.FormatInt(in.ResourceVersion, 10)
	}
	if !in.CreationTimestamp.IsZero() {
		out.CreationTimestamp = metav1.NewTime(in.CreationTimestamp)
	}
	if in.DeletionTimestamp != nil {
		t := metav1.NewTime(*in.DeletionTimestamp)
		out.DeletionTimestamp = &t
	}
	if len(in.OwnerReferences) > 0 {
		out.OwnerReferences = make([]metav1.OwnerReference, len(in.OwnerReferences))
		for i, ref := range in.OwnerReferences {
			out.OwnerReferences[i] = metav1.OwnerReference{
				APIVersion: ref.APIVersion,
				Kind:       ref.Kind,
				Name:       ref.Name,
				UID:        types.UID(ref.UID),
			}
			if ref.Controller {
				controller := true
				out.OwnerReferences[i].Controller = &controller
			}
		}
	}
	return out
}

func objectMetaFromK8s(in metav1.ObjectMeta) api.ObjectMeta {
	out := api.ObjectMeta{
		Name:        in.Name,
		Namespace:   in.Namespace,
		UID:         string(in.UID),
		Labels:      in.Labels,
		Annotations: in.Annotations,
		Finalizers:  in.Finalizers,
		Generation:  in.Generation,
	}
	if in.ResourceVersion != "" {
		out.ResourceVersion, _ = strconv.ParseInt(in.ResourceVersion, 10, 64)
	}
	if !in.CreationTimestamp.IsZero() {
		out.CreationTimestamp = in.CreationTimestamp.Time
	}
	if in.DeletionTimestamp != nil {
		t := in.DeletionTimestamp.Time
		out.DeletionTimestamp = &t
	}
	if len(in.OwnerReferences) > 0 {
		out.OwnerReferences = make([]api.OwnerReference, len(in.OwnerReferences))
		for i, ref := range in.OwnerReferences {
			out.OwnerReferences[i] = api.OwnerReference{
				APIVersion: ref.APIVersion,
				Kind:       ref.Kind,
				Name:       ref.Name,
				UID:        string(ref.UID),
				Controller: ref.Controller != nil && *ref.Controller,
			}
		}
	}
	return out
}

// --- Conditions ---

func conditionsToK8s(in []api.Condition) []metav1.Condition {
	if in == nil {
		return nil
	}
	out := make([]metav1.Condition, len(in))
	for i, c := range in {
		out[i] = metav1.Condition{
			Type:               c.Type,
			Status:             metav1.ConditionStatus(c.Status),
			LastTransitionTime: metav1.NewTime(c.LastTransitionTime),
			Reason:             c.Reason,
			Message:            c.Message,
		}
	}
	return out
}

func conditionsFromK8s(in []metav1.Condition) []api.Condition {
	if in == nil {
		return nil
	}
	out := make([]api.Condition, len(in))
	for i, c := range in {
		out[i] = api.Condition{
			Type:               c.Type,
			Status:             string(c.Status),
			LastTransitionTime: c.LastTransitionTime.Time,
			Reason:             c.Reason,
			Message:            c.Message,
		}
	}
	return out
}

// --- Duration helpers ---

func durationToString(d api.Duration) string {
	if d.Duration == 0 {
		return ""
	}
	return d.Duration.String()
}

func durationFromString(s string) api.Duration {
	if s == "" {
		return api.Duration{}
	}
	d, _ := time.ParseDuration(s)
	return api.Duration{Duration: d}
}

// --- Phase helpers ---

func phaseToString(p api.Phase) string {
	return string(p)
}

func phaseFromString(s string) api.Phase {
	return api.Phase(s)
}

// --- LocalObjectReference ---

func localRefToK8s(name string) k8s.LocalObjectReference {
	return k8s.LocalObjectReference{Name: name}
}

func localRefFromK8s(ref k8s.LocalObjectReference) string {
	return ref.Name
}

// --- Time helpers ---

func timeToMetav1(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	mt := metav1.NewTime(*t)
	return &mt
}

func timeFromMetav1(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	tt := t.Time
	return &tt
}
