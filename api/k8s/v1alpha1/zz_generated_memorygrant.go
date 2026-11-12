// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import runtime "k8s.io/apimachinery/pkg/runtime"

func (in *MemoryGrant) DeepCopyInto(out *MemoryGrant) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
}

func (in *MemoryGrant) DeepCopy() *MemoryGrant {
	if in == nil {
		return nil
	}
	out := new(MemoryGrant)
	in.DeepCopyInto(out)
	return out
}

func (in *MemoryGrant) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MemoryGrantList) DeepCopyInto(out *MemoryGrantList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]MemoryGrant, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *MemoryGrantList) DeepCopy() *MemoryGrantList {
	if in == nil {
		return nil
	}
	out := new(MemoryGrantList)
	in.DeepCopyInto(out)
	return out
}

func (in *MemoryGrantList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
