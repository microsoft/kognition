// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

func (in *Memory) DeepCopyInto(out *Memory) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Memory) DeepCopy() *Memory {
	if in == nil {
		return nil
	}
	out := new(Memory)
	in.DeepCopyInto(out)
	return out
}

func (in *Memory) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MemoryStatus) DeepCopyInto(out *MemoryStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
}

func (in *MemoryList) DeepCopyInto(out *MemoryList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Memory, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *MemoryList) DeepCopy() *MemoryList {
	if in == nil {
		return nil
	}
	out := new(MemoryList)
	in.DeepCopyInto(out)
	return out
}

func (in *MemoryList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MemoryRecord) DeepCopyInto(out *MemoryRecord) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *MemoryRecord) DeepCopy() *MemoryRecord {
	if in == nil {
		return nil
	}
	out := new(MemoryRecord)
	in.DeepCopyInto(out)
	return out
}

func (in *MemoryRecord) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MemoryRecordSpec) DeepCopyInto(out *MemoryRecordSpec) {
	*out = *in
	if in.Metadata != nil {
		out.Metadata = make(map[string]string, len(in.Metadata))
		for key, value := range in.Metadata {
			out.Metadata[key] = value
		}
	}
	if in.Provenance != nil {
		out.Provenance = new(MemoryProvenance)
		in.Provenance.DeepCopyInto(out.Provenance)
	}
}

func (in *MemoryProvenance) DeepCopyInto(out *MemoryProvenance) {
	*out = *in
	in.Timestamp.DeepCopyInto(&out.Timestamp)
	if in.DerivedFromRecordNames != nil {
		out.DerivedFromRecordNames = append([]string(nil), in.DerivedFromRecordNames...)
	}
}

func (in *MemoryRecordStatus) DeepCopyInto(out *MemoryRecordStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
}

func (in *MemoryRecordList) DeepCopyInto(out *MemoryRecordList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]MemoryRecord, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *MemoryRecordList) DeepCopy() *MemoryRecordList {
	if in == nil {
		return nil
	}
	out := new(MemoryRecordList)
	in.DeepCopyInto(out)
	return out
}

func (in *MemoryRecordList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
