// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Hand-written deepcopy methods for ModelAutoscaler types.
// controller-gen v0.21.0 panics on this package; these are equivalent to
// what controller-gen would produce. Keep in sync with modelautoscaler_types.go.

package v1alpha1

import (
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *ModelAutoscaler) DeepCopyInto(out *ModelAutoscaler) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy.
func (in *ModelAutoscaler) DeepCopy() *ModelAutoscaler {
	if in == nil {
		return nil
	}
	out := new(ModelAutoscaler)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a deep copy as runtime.Object.
func (in *ModelAutoscaler) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *ModelAutoscalerSpec) DeepCopyInto(out *ModelAutoscalerSpec) {
	*out = *in
	out.AgentRef = in.AgentRef
	out.PreferredModelRef = in.PreferredModelRef
}

// DeepCopy returns a deep copy.
func (in *ModelAutoscalerSpec) DeepCopy() *ModelAutoscalerSpec {
	if in == nil {
		return nil
	}
	out := new(ModelAutoscalerSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *ModelAutoscalerStatus) DeepCopyInto(out *ModelAutoscalerStatus) {
	*out = *in
	if in.LastScaleTime != nil {
		out.LastScaleTime = in.LastScaleTime.DeepCopy()
	}
	if in.Conditions != nil {
		out.Conditions = make([]v1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

// DeepCopy returns a deep copy.
func (in *ModelAutoscalerStatus) DeepCopy() *ModelAutoscalerStatus {
	if in == nil {
		return nil
	}
	out := new(ModelAutoscalerStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *ModelAutoscalerList) DeepCopyInto(out *ModelAutoscalerList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ModelAutoscaler, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy.
func (in *ModelAutoscalerList) DeepCopy() *ModelAutoscalerList {
	if in == nil {
		return nil
	}
	out := new(ModelAutoscalerList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a deep copy as runtime.Object.
func (in *ModelAutoscalerList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
