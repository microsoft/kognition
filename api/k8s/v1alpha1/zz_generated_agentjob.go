// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Hand-written deepcopy methods for AgentJob types.
// controller-gen v0.21.0 panics on this package; these are equivalent to
// what controller-gen would produce. Keep in sync with the AgentJob*
// structs in agentjob_types.go.

package v1alpha1

import (
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *AgentJob) DeepCopyInto(out *AgentJob) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy.
func (in *AgentJob) DeepCopy() *AgentJob {
	if in == nil {
		return nil
	}
	out := new(AgentJob)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a deep copy as runtime.Object.
func (in *AgentJob) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *AgentJobSpec) DeepCopyInto(out *AgentJobSpec) {
	*out = *in
	if in.AgentRef != nil {
		out.AgentRef = new(LocalObjectReference)
		*out.AgentRef = *in.AgentRef
	}
	if in.AgentRevision != nil {
		out.AgentRevision = new(int64)
		*out.AgentRevision = *in.AgentRevision
	}
	if in.AgentSpec != nil {
		out.AgentSpec = new(AgentSpec)
		in.AgentSpec.DeepCopyInto(out.AgentSpec)
	}
	if in.BackoffLimit != nil {
		out.BackoffLimit = new(int32)
		*out.BackoffLimit = *in.BackoffLimit
	}
	if in.MemoryClaimRef != nil {
		out.MemoryClaimRef = new(LocalObjectReference)
		*out.MemoryClaimRef = *in.MemoryClaimRef
	}
}

// DeepCopy returns a deep copy.
func (in *AgentJobSpec) DeepCopy() *AgentJobSpec {
	if in == nil {
		return nil
	}
	out := new(AgentJobSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *AgentJobStatus) DeepCopyInto(out *AgentJobStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]v1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
	if in.StartTime != nil {
		out.StartTime = in.StartTime.DeepCopy()
	}
	if in.CompletionTime != nil {
		out.CompletionTime = in.CompletionTime.DeepCopy()
	}
	if in.TokenUsage != nil {
		out.TokenUsage = new(TokenUsage)
		*out.TokenUsage = *in.TokenUsage
	}
}

// DeepCopy returns a deep copy.
func (in *AgentJobStatus) DeepCopy() *AgentJobStatus {
	if in == nil {
		return nil
	}
	out := new(AgentJobStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *AgentJobList) DeepCopyInto(out *AgentJobList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]AgentJob, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy.
func (in *AgentJobList) DeepCopy() *AgentJobList {
	if in == nil {
		return nil
	}
	out := new(AgentJobList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a deep copy as runtime.Object.
func (in *AgentJobList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
