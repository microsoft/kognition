// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import runtime "k8s.io/apimachinery/pkg/runtime"

func (in *ModelGrant) DeepCopyInto(out *ModelGrant) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
}

func (in *ModelGrant) DeepCopy() *ModelGrant {
	if in == nil {
		return nil
	}
	out := new(ModelGrant)
	in.DeepCopyInto(out)
	return out
}

func (in *ModelGrant) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ModelGrantList) DeepCopyInto(out *ModelGrantList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ModelGrant, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ModelGrantList) DeepCopy() *ModelGrantList {
	if in == nil {
		return nil
	}
	out := new(ModelGrantList)
	in.DeepCopyInto(out)
	return out
}

func (in *ModelGrantList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
