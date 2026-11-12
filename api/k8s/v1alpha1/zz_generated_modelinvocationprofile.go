// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "k8s.io/apimachinery/pkg/runtime"

func (in *ModelInvocationProfile) DeepCopyInto(out *ModelInvocationProfile) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

func (in *ModelInvocationProfile) DeepCopy() *ModelInvocationProfile {
	if in == nil {
		return nil
	}
	out := new(ModelInvocationProfile)
	in.DeepCopyInto(out)
	return out
}

func (in *ModelInvocationProfile) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

func (in *ModelInvocationProfileList) DeepCopyInto(out *ModelInvocationProfileList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ModelInvocationProfile, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ModelInvocationProfileList) DeepCopy() *ModelInvocationProfileList {
	if in == nil {
		return nil
	}
	out := new(ModelInvocationProfileList)
	in.DeepCopyInto(out)
	return out
}

func (in *ModelInvocationProfileList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}
