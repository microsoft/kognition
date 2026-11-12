// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import runtime "k8s.io/apimachinery/pkg/runtime"

func (in *AgentCommunicationPolicy) DeepCopyInto(out *AgentCommunicationPolicy) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Spec.AllowFrom = append([]LocalObjectReference(nil), in.Spec.AllowFrom...)
	out.Spec.AllowTo = append([]LocalObjectReference(nil), in.Spec.AllowTo...)
}

func (in *AgentCommunicationPolicy) DeepCopy() *AgentCommunicationPolicy {
	if in == nil {
		return nil
	}
	out := new(AgentCommunicationPolicy)
	in.DeepCopyInto(out)
	return out
}

func (in *AgentCommunicationPolicy) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AgentCommunicationPolicyList) DeepCopyInto(out *AgentCommunicationPolicyList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]AgentCommunicationPolicy, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *AgentCommunicationPolicyList) DeepCopy() *AgentCommunicationPolicyList {
	if in == nil {
		return nil
	}
	out := new(AgentCommunicationPolicyList)
	in.DeepCopyInto(out)
	return out
}

func (in *AgentCommunicationPolicyList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
