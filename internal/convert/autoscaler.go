// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func AgentAutoscalerToK8s(in *api.AgentAutoscaler) *k8s.AgentAutoscaler {
	out := &k8s.AgentAutoscaler{
		TypeMeta:   metav1.TypeMeta{Kind: "AgentAutoscaler", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.AgentAutoscalerSpec{
			AgentRef: localRefToK8s(in.Spec.AgentRef.Name), MinReplicas: in.Spec.MinReplicas, MaxReplicas: in.Spec.MaxReplicas,
		},
		Status: k8s.AgentAutoscalerStatus{
			CurrentReplicas: in.Status.CurrentReplicas, DesiredReplicas: in.Status.DesiredReplicas,
			Conditions: conditionsToK8s(in.Status.Conditions),
		},
	}
	for _, metric := range in.Spec.Metrics {
		out.Spec.Metrics = append(out.Spec.Metrics, k8s.AutoscalerMetric{Type: k8s.MetricType(metric.Type), TargetValue: metric.TargetValue})
	}
	if in.Spec.ScaleUpPolicy != nil {
		out.Spec.ScaleUpPolicy = &k8s.ScalingPolicy{StabilizationWindowSeconds: in.Spec.ScaleUpPolicy.StabilizationWindowSeconds}
	}
	if in.Spec.ScaleDownPolicy != nil {
		out.Spec.ScaleDownPolicy = &k8s.ScalingPolicy{StabilizationWindowSeconds: in.Spec.ScaleDownPolicy.StabilizationWindowSeconds}
	}
	if in.Status.LastScaleTime != nil {
		t := metav1.NewTime(in.Status.LastScaleTime.UTC())
		out.Status.LastScaleTime = &t
	}
	return out
}

func AgentAutoscalerFromK8s(in *k8s.AgentAutoscaler) *api.AgentAutoscaler {
	out := &api.AgentAutoscaler{
		TypeMeta: api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion}, ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec:   api.AgentAutoscalerSpec{AgentRef: api.LocalObjectReference{Name: localRefFromK8s(in.Spec.AgentRef)}, MinReplicas: in.Spec.MinReplicas, MaxReplicas: in.Spec.MaxReplicas},
		Status: api.AgentAutoscalerStatus{CurrentReplicas: in.Status.CurrentReplicas, DesiredReplicas: in.Status.DesiredReplicas, Conditions: conditionsFromK8s(in.Status.Conditions)},
	}
	for _, metric := range in.Spec.Metrics {
		out.Spec.Metrics = append(out.Spec.Metrics, api.AutoscalerMetric{Type: api.MetricType(metric.Type), TargetValue: metric.TargetValue})
	}
	if in.Spec.ScaleUpPolicy != nil {
		out.Spec.ScaleUpPolicy = &api.ScalingPolicy{StabilizationWindowSeconds: in.Spec.ScaleUpPolicy.StabilizationWindowSeconds}
	}
	if in.Spec.ScaleDownPolicy != nil {
		out.Spec.ScaleDownPolicy = &api.ScalingPolicy{StabilizationWindowSeconds: in.Spec.ScaleDownPolicy.StabilizationWindowSeconds}
	}
	if in.Status.LastScaleTime != nil {
		t := in.Status.LastScaleTime.Time.UTC()
		out.Status.LastScaleTime = &t
	}
	return out
}

func ModelAutoscalerToK8s(in *api.ModelAutoscaler) *k8s.ModelAutoscaler {
	out := &k8s.ModelAutoscaler{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ModelAutoscaler",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.ModelAutoscalerSpec{
			AgentRef:                   localRefToK8s(in.Spec.AgentRef.Name),
			PreferredModelRef:          localRefToK8s(in.Spec.PreferredModelRef.Name),
			StabilizationWindowSeconds: in.Spec.StabilizationWindowSeconds,
		},
		Status: k8s.ModelAutoscalerStatus{
			CurrentModelRef: in.Status.CurrentModelRef,
			DesiredModelRef: in.Status.DesiredModelRef,
			Conditions:      conditionsToK8s(in.Status.Conditions),
		},
	}
	if in.Status.LastScaleTime != nil {
		t := metav1.NewTime(in.Status.LastScaleTime.UTC())
		out.Status.LastScaleTime = &t
	}
	return out
}

func ModelAutoscalerFromK8s(in *k8s.ModelAutoscaler) *api.ModelAutoscaler {
	out := &api.ModelAutoscaler{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.ModelAutoscalerSpec{
			AgentRef:                   api.LocalObjectReference{Name: localRefFromK8s(in.Spec.AgentRef)},
			PreferredModelRef:          api.LocalObjectReference{Name: localRefFromK8s(in.Spec.PreferredModelRef)},
			StabilizationWindowSeconds: in.Spec.StabilizationWindowSeconds,
		},
		Status: api.ModelAutoscalerStatus{
			CurrentModelRef: in.Status.CurrentModelRef,
			DesiredModelRef: in.Status.DesiredModelRef,
			Conditions:      conditionsFromK8s(in.Status.Conditions),
		},
	}
	if in.Status.LastScaleTime != nil {
		t := in.Status.LastScaleTime.Time.UTC()
		out.Status.LastScaleTime = &t
	}
	return out
}
