// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

func (in *AgentAutoscaler) DeepCopyInto(out *AgentAutoscaler) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Spec.Metrics = append([]AutoscalerMetric(nil), in.Spec.Metrics...)
	if in.Spec.ScaleUpPolicy != nil {
		out.Spec.ScaleUpPolicy = new(ScalingPolicy)
		*out.Spec.ScaleUpPolicy = *in.Spec.ScaleUpPolicy
	}
	if in.Spec.ScaleDownPolicy != nil {
		out.Spec.ScaleDownPolicy = new(ScalingPolicy)
		*out.Spec.ScaleDownPolicy = *in.Spec.ScaleDownPolicy
	}
	out.Status = in.Status
	if in.Status.LastScaleTime != nil {
		out.Status.LastScaleTime = in.Status.LastScaleTime.DeepCopy()
	}
	out.Status.Conditions = append([]metav1.Condition(nil), in.Status.Conditions...)
}

func (in *AgentAutoscaler) DeepCopy() *AgentAutoscaler {
	if in == nil {
		return nil
	}
	out := new(AgentAutoscaler)
	in.DeepCopyInto(out)
	return out
}

func (in *AgentAutoscaler) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AgentAutoscalerList) DeepCopyInto(out *AgentAutoscalerList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]AgentAutoscaler, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *AgentAutoscalerList) DeepCopy() *AgentAutoscalerList {
	if in == nil {
		return nil
	}
	out := new(AgentAutoscalerList)
	in.DeepCopyInto(out)
	return out
}

func (in *AgentAutoscalerList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
