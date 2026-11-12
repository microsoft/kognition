// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Hand-written deepcopy methods for KnowledgeBaseIndexRun and
// KnowledgeBaseIndexSchedule. controller-gen v0.21.0 panics on this package;
// these are equivalent to what controller-gen would produce. Keep in sync
// with the types in kbindex_types.go.

package v1alpha1

import (
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// ---- KnowledgeBaseIndexRunSpec ----

func (in *KnowledgeBaseIndexRunSpec) DeepCopyInto(out *KnowledgeBaseIndexRunSpec) {
	*out = *in
	if in.TTLSecondsAfterFinished != nil {
		out.TTLSecondsAfterFinished = new(int32)
		*out.TTLSecondsAfterFinished = *in.TTLSecondsAfterFinished
	}
}

func (in *KnowledgeBaseIndexRunSpec) DeepCopy() *KnowledgeBaseIndexRunSpec {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseIndexRunSpec)
	in.DeepCopyInto(out)
	return out
}

// ---- KnowledgeBaseIndexRunStatus ----

func (in *KnowledgeBaseIndexRunStatus) DeepCopyInto(out *KnowledgeBaseIndexRunStatus) {
	*out = *in
	if in.BackendBinding != nil {
		in, out := &in.BackendBinding, &out.BackendBinding
		*out = new(KnowledgeBackendBinding)
		**out = **in
		if (*in).EmbeddingModelBinding != nil {
			(*out).EmbeddingModelBinding = new(ModelVersionBinding)
			*(*out).EmbeddingModelBinding = *(*in).EmbeddingModelBinding
		}
	}
	if in.Conditions != nil {
		out.Conditions = make([]v1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *KnowledgeBaseIndexRunStatus) DeepCopy() *KnowledgeBaseIndexRunStatus {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseIndexRunStatus)
	in.DeepCopyInto(out)
	return out
}

// ---- KnowledgeBaseIndexRun ----

func (in *KnowledgeBaseIndexRun) DeepCopyInto(out *KnowledgeBaseIndexRun) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *KnowledgeBaseIndexRun) DeepCopy() *KnowledgeBaseIndexRun {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseIndexRun)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBaseIndexRun) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// ---- KnowledgeBaseIndexRunList ----

func (in *KnowledgeBaseIndexRunList) DeepCopyInto(out *KnowledgeBaseIndexRunList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]KnowledgeBaseIndexRun, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *KnowledgeBaseIndexRunList) DeepCopy() *KnowledgeBaseIndexRunList {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseIndexRunList)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBaseIndexRunList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// ---- KnowledgeBaseIndexScheduleSpec ----

func (in *KnowledgeBaseIndexScheduleSpec) DeepCopyInto(out *KnowledgeBaseIndexScheduleSpec) {
	*out = *in
	if in.Suspend != nil {
		out.Suspend = new(bool)
		*out.Suspend = *in.Suspend
	}
	if in.SuccessfulRunsHistoryLimit != nil {
		out.SuccessfulRunsHistoryLimit = new(int32)
		*out.SuccessfulRunsHistoryLimit = *in.SuccessfulRunsHistoryLimit
	}
	if in.FailedRunsHistoryLimit != nil {
		out.FailedRunsHistoryLimit = new(int32)
		*out.FailedRunsHistoryLimit = *in.FailedRunsHistoryLimit
	}
	if in.StartingDeadlineSeconds != nil {
		out.StartingDeadlineSeconds = new(int64)
		*out.StartingDeadlineSeconds = *in.StartingDeadlineSeconds
	}
	in.RunTemplate.DeepCopyInto(&out.RunTemplate)
}

func (in *KnowledgeBaseIndexScheduleSpec) DeepCopy() *KnowledgeBaseIndexScheduleSpec {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseIndexScheduleSpec)
	in.DeepCopyInto(out)
	return out
}

// ---- KnowledgeBaseIndexScheduleStatus ----

func (in *KnowledgeBaseIndexScheduleStatus) DeepCopyInto(out *KnowledgeBaseIndexScheduleStatus) {
	*out = *in
	if in.Active != nil {
		out.Active = make([]LocalObjectReference, len(in.Active))
		copy(out.Active, in.Active)
	}
	if in.Conditions != nil {
		out.Conditions = make([]v1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *KnowledgeBaseIndexScheduleStatus) DeepCopy() *KnowledgeBaseIndexScheduleStatus {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseIndexScheduleStatus)
	in.DeepCopyInto(out)
	return out
}

// ---- KnowledgeBaseIndexSchedule ----

func (in *KnowledgeBaseIndexSchedule) DeepCopyInto(out *KnowledgeBaseIndexSchedule) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *KnowledgeBaseIndexSchedule) DeepCopy() *KnowledgeBaseIndexSchedule {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseIndexSchedule)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBaseIndexSchedule) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// ---- KnowledgeBaseIndexScheduleList ----

func (in *KnowledgeBaseIndexScheduleList) DeepCopyInto(out *KnowledgeBaseIndexScheduleList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]KnowledgeBaseIndexSchedule, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *KnowledgeBaseIndexScheduleList) DeepCopy() *KnowledgeBaseIndexScheduleList {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseIndexScheduleList)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBaseIndexScheduleList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
